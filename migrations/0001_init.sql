-- 0001_init.sql — M0 schema baseline.
--
-- Design notes that are easy to get wrong later:
--   * Every user-visible object is addressed by UUID, never by path. Paths are a
--     rendering of the tree, not an identity, so moving a file never changes
--     who may read it.
--   * The audit schema is written by a dedicated DB role with INSERT only. An
--     application that can UPDATE its own audit log has no audit log.
--   * Heavy tables (audit_events) are partitioned from day one; retrofitting
--     partitioning on a table with 10^8 rows is an outage.

BEGIN;

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------- identity --

CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username        citext NOT NULL UNIQUE,
    email           citext UNIQUE,
    display_name    text NOT NULL DEFAULT '',
    -- Pocket ID identity: sub is stable forever, so (issuer, sub) is the key.
    idp_issuer      text,
    idp_subject     text,
    role            text NOT NULL DEFAULT 'guest'
                    CHECK (role IN ('admin','user','guest')),
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','disabled','pending')),
    quota_bytes     bigint NOT NULL DEFAULT 10737418240 CHECK (quota_bytes >= 0),
    used_bytes      bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    is_breakglass   boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    disabled_at     timestamptz,
    UNIQUE (idp_issuer, idp_subject)
);

-- A SCIM pause must never leave two enabled admins behind: enforced in app code,
-- indexed here for the "how many admins exist" query.
CREATE INDEX users_role_idx ON users (role) WHERE status = 'active';

CREATE TABLE groups (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            citext NOT NULL UNIQUE,      -- matches the IdP group name
    display_name    text NOT NULL DEFAULT '',
    quota_bytes     bigint CHECK (quota_bytes >= 0),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id        uuid NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            text NOT NULL DEFAULT 'viewer'
                    CHECK (role IN ('viewer','editor','manager')),
    added_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_idx ON group_members (user_id);

-- Maps a Pocket ID group *name* to a local role. A table rather than config so
-- that a SCIM rename can be applied without a redeploy.
CREATE TABLE group_role_map (
    idp_group_name  citext PRIMARY KEY,
    role            text NOT NULL CHECK (role IN ('admin','user','guest')),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------ session --

CREATE TABLE sessions (
    id              uuid NOT NULL,               -- jti, part of the ticket
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    ip              inet,
    user_agent      text NOT NULL DEFAULT '',
    auth_method     text NOT NULL CHECK (auth_method IN ('oidc','breakglass')),
    revoked_at      timestamptz,
    PRIMARY KEY (id, expires_at)                 -- partition key must be in PK
) PARTITION BY RANGE (expires_at);

CREATE TABLE sessions_default PARTITION OF sessions DEFAULT;
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;

-- App passwords are 256-bit random secrets, so they are indexed by SHA-256.
-- Argon2 here would add ~60 ms to *every* WebDAV request and buys nothing
-- against a 2^128 keyspace.
CREATE TABLE app_passwords (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label           text NOT NULL,
    prefix          text NOT NULL,               -- first chars, for display only
    token_hash      bytea NOT NULL UNIQUE,
    scope           text NOT NULL DEFAULT 'rw' CHECK (scope IN ('ro','rw')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,
    last_used_at    timestamptz,
    last_used_ip    inet,
    revoked_at      timestamptz
);
CREATE INDEX app_passwords_user_idx ON app_passwords (user_id) WHERE revoked_at IS NULL;

-- ------------------------------------------------------------------ storage --

CREATE TABLE blobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256          bytea NOT NULL UNIQUE,
    size            bigint NOT NULL CHECK (size >= 0),
    backend         text NOT NULL,
    refcount        integer NOT NULL DEFAULT 0 CHECK (refcount >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Set when the last reference is dropped; the vacuum task deletes the bytes
    -- after a grace period so an accidental delete is recoverable.
    collect_after   timestamptz
);
CREATE INDEX blobs_collect_idx ON blobs (collect_after) WHERE collect_after IS NOT NULL;

-- The tree. parent_id NULL marks a user's (or group's) root.
CREATE TABLE nodes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id       uuid REFERENCES nodes(id),
    owner_id        uuid NOT NULL REFERENCES users(id),
    -- Group-owned spaces set owner_group_id; owner_id is then the creator.
    owner_group_id  uuid REFERENCES groups(id),
    name            text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('file','dir')),
    size            bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    mime            text NOT NULL DEFAULT '',
    etag            text NOT NULL DEFAULT '',
    blob_id         uuid REFERENCES blobs(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    modified_at     timestamptz NOT NULL DEFAULT now(),
    trashed_at      timestamptz,
    trashed_from    uuid REFERENCES nodes(id),
    CHECK (kind <> 'file' OR blob_id IS NOT NULL),
    -- A root has an empty name and no parent; every other node needs a real
    -- name. Expressing both here keeps "a node without a name" unrepresentable
    -- rather than relying on every writer remembering the exception.
    CHECK (
        (parent_id IS NULL AND name = '') OR
        (parent_id IS NOT NULL AND length(name) BETWEEN 1 AND 255)
    ),
    -- No NUL, no path separators: a name is a single path component by
    -- construction, which removes the entire traversal bug class at the schema.
    CHECK (name !~ '[/\\]')
);

-- The partial unique index is load-bearing: without it, a soft-deleted file
-- blocks re-creating a file with the same name in the same directory.
CREATE UNIQUE INDEX nodes_sibling_name_uniq
    ON nodes (parent_id, name) WHERE trashed_at IS NULL;

-- One root per owner. EnsureRoot relies on this as an ON CONFLICT target so
-- two simultaneous first logins converge instead of creating two roots.
CREATE UNIQUE INDEX nodes_root_uniq ON nodes (owner_id)
    WHERE parent_id IS NULL AND trashed_at IS NULL;

CREATE INDEX nodes_parent_idx ON nodes (parent_id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_owner_idx ON nodes (owner_id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_blob_idx ON nodes (blob_id);
CREATE INDEX nodes_trash_idx ON nodes (trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TABLE versions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    version_no      integer NOT NULL,
    blob_id         uuid NOT NULL REFERENCES blobs(id),
    size            bigint NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid REFERENCES users(id),
    UNIQUE (node_id, version_no)
);
CREATE INDEX versions_node_idx ON versions (node_id, version_no DESC);

-- WebDAV dead properties. Windows Explorer and macOS Finder both store
-- per-file metadata here (Win32LastModifiedTime, AppleDouble) and misbehave
-- when it does not survive a reconnect.
CREATE TABLE dead_props (
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    namespace       text NOT NULL,
    name            text NOT NULL,
    value           text NOT NULL,
    PRIMARY KEY (node_id, namespace, name)
);

-- WebDAV locks must be durable. An in-memory lock system silently breaks under
-- restart or a second process, which corrupts Office co-authoring.
CREATE TABLE lock_tokens (
    token           text PRIMARY KEY,
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_id         uuid REFERENCES users(id),
    owner_xml       text NOT NULL DEFAULT '',
    depth           text NOT NULL CHECK (depth IN ('0','infinity')),
    timeout_sec     integer NOT NULL CHECK (timeout_sec > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL
);
CREATE INDEX lock_tokens_node_idx ON lock_tokens (node_id);
CREATE INDEX lock_tokens_expiry_idx ON lock_tokens (expires_at);

-- Group-shared spaces: an explicit grant of permission bits on a subtree root.
CREATE TABLE acl_entries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_id         uuid REFERENCES users(id) ON DELETE CASCADE,
    group_id        uuid REFERENCES groups(id) ON DELETE CASCADE,
    perms           text NOT NULL,               -- e.g. 'read,write'
    inherit         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((user_id IS NULL) <> (group_id IS NULL))
);
CREATE INDEX acl_entries_node_idx ON acl_entries (node_id);
CREATE INDEX acl_entries_user_idx ON acl_entries (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX acl_entries_group_idx ON acl_entries (group_id) WHERE group_id IS NOT NULL;

-- ------------------------------------------------------------------- shares --

CREATE TABLE shares (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Only the SHA-256 of the URL token is stored: a database dump must not
    -- yield working share links.
    token_hash      bytea NOT NULL UNIQUE,
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    owner_id        uuid NOT NULL REFERENCES users(id),
    kind            text NOT NULL CHECK (kind IN ('link','email','request')),
    label           text NOT NULL DEFAULT '',
    password_hash   bytea,                       -- Argon2id, human-chosen
    expires_at      timestamptz,
    max_downloads   integer CHECK (max_downloads IS NULL OR max_downloads > 0),
    download_count  integer NOT NULL DEFAULT 0 CHECK (download_count >= 0),
    max_uploads     integer CHECK (max_uploads IS NULL OR max_uploads > 0),
    upload_count    integer NOT NULL DEFAULT 0 CHECK (upload_count >= 0),
    max_bytes_per_file bigint,
    allowed_mime    text[],
    -- Permission bits granted through this share: 'read', 'create'.
    permissions     text[] NOT NULL DEFAULT '{read}',
    -- File requests hide the directory listing: upload-only by construction.
    hide_contents   boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    CHECK (kind <> 'request' OR hide_contents),
    CHECK (NOT ('create' = ANY(permissions)) OR hide_contents OR kind = 'link')
);
CREATE INDEX shares_node_idx ON shares (node_id);
CREATE INDEX shares_owner_idx ON shares (owner_id) WHERE revoked_at IS NULL;

CREATE TABLE share_recipients (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    share_id        uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    email           citext NOT NULL,
    user_id         uuid REFERENCES users(id),
    invited_at      timestamptz NOT NULL DEFAULT now(),
    verified_at     timestamptz,
    first_access_at timestamptz,
    last_access_at  timestamptz,
    revoked_at      timestamptz,
    UNIQUE (share_id, email)
);

-- ------------------------------------------------ audit (INSERT-only role) --

CREATE SCHEMA IF NOT EXISTS audit;

CREATE TABLE audit.events (
    id              bigint GENERATED ALWAYS AS IDENTITY,
    ts              timestamptz NOT NULL DEFAULT now(),
    actor_kind      text NOT NULL,
    actor_user_id   uuid,
    actor_email     citext,
    actor_ip        inet,
    user_agent      text NOT NULL DEFAULT '',
    action          text NOT NULL,
    target_kind     text NOT NULL,
    target_id       text NOT NULL DEFAULT '',
    share_id        uuid,
    result          text NOT NULL CHECK (result IN ('ok','denied','error')),
    bytes           bigint,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    prev_hash       bytea NOT NULL,
    hash            bytea NOT NULL,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);

-- Monthly partitions are created by a maintenance task; the default partition
-- guarantees no write is ever lost because a partition was missing.
CREATE TABLE audit.events_default PARTITION OF audit.events DEFAULT;

CREATE INDEX audit_events_ts_idx ON audit.events (ts DESC);
CREATE INDEX audit_events_share_idx ON audit.events (share_id) WHERE share_id IS NOT NULL;
CREATE INDEX audit_events_actor_idx ON audit.events (actor_user_id, ts DESC);
CREATE INDEX audit_events_action_idx ON audit.events (action, ts DESC);

-- The daily anchor: the chain head for a day, published outside the database.
CREATE TABLE audit.anchors (
    day             date PRIMARY KEY,
    head_hash       bytea NOT NULL,
    published_at    timestamptz NOT NULL DEFAULT now(),
    destination     text NOT NULL DEFAULT ''
);

-- The application role may append to the audit log but never rewrite it.
-- Enforced here rather than by convention because the convention is exactly what
-- gets dropped under deadline pressure.
--
-- Two roles, deliberately:
--   newcloud_app  -- the request-serving connection. SELECT/INSERT/UPDATE on
--                    business tables, but only INSERT on audit.events.
--   (admin/owner) -- migrations and maintenance, not used at runtime.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newcloud_app') THEN
        CREATE ROLE newcloud_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newcloud_audit_writer') THEN
        CREATE ROLE newcloud_audit_writer NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO newcloud_app;
GRANT USAGE ON SCHEMA audit TO newcloud_app;

-- Business tables: full DML for the application.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO newcloud_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO newcloud_app;

-- Audit tables: INSERT only. SELECT is granted so the audit UI and the chain
-- verifier can read; UPDATE/DELETE/TRUNCATE are never granted.
GRANT INSERT, SELECT ON audit.events TO newcloud_app;
GRANT INSERT, SELECT ON audit.anchors TO newcloud_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit.events FROM newcloud_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit.anchors FROM newcloud_app;

-- Partitions inherit nothing from the parent for permissions; grants on a
-- partitioned table do not automatically apply to partitions created later, so
-- the maintenance task that adds monthly partitions must re-run these grants.
-- This function exists so that task has exactly one place to call.
CREATE OR REPLACE FUNCTION audit.grant_partition_privileges() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    part record;
BEGIN
    FOR part IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'audit' AND c.relispartition
    LOOP
        EXECUTE format('GRANT INSERT, SELECT ON audit.%I TO newcloud_app', part.relname);
    END LOOP;
END
$$;

-- Default privileges so tables added by later migrations are usable without a
-- second GRANT, while audit objects stay append-only.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO newcloud_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO newcloud_app;

-- ------------------------------------------------------------- outbox/work --

-- Async work is queued transactionally with the business change, so "the file
-- is visible but never scanned" cannot happen.
--
-- The table is the queue. There is deliberately no external MQ: workers claim
-- rows with FOR UPDATE SKIP LOCKED, which is correct for one worker and for
-- twenty. That is what keeps "single host today, several replicas later" a
-- deployment change instead of a rewrite.
--
--   BEGIN;
--   SELECT id FROM jobs
--    WHERE completed_at IS NULL AND run_after <= now()
--      AND (locked_until IS NULL OR locked_until < now())
--    ORDER BY run_after
--    LIMIT 10
--      FOR UPDATE SKIP LOCKED;
--   UPDATE jobs SET locked_until = now() + interval '5 minutes', attempts = attempts + 1
--    WHERE id = ANY($1);
--   COMMIT;
--
-- locked_until is a lease, not a flag: a worker that dies mid-job leaves the row
-- claimable again after the lease expires, so a crash cannot strand work.
CREATE TABLE jobs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind            text NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Partial unique key for idempotent enqueue: two concurrent uploads of the
    -- same file must not queue two identical scans.
    dedupe_key      text,
    run_after       timestamptz NOT NULL DEFAULT now(),
    attempts        integer NOT NULL DEFAULT 0,
    max_attempts    integer NOT NULL DEFAULT 5,
    last_error      text,
    locked_until    timestamptz,
    locked_by       text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz
);

-- The claim query filters on exactly these columns.
CREATE INDEX jobs_pending_idx ON jobs (run_after)
    WHERE completed_at IS NULL;
CREATE UNIQUE INDEX jobs_dedupe_uniq ON jobs (kind, dedupe_key)
    WHERE completed_at IS NULL AND dedupe_key IS NOT NULL;

-- SCIM changes are staged first so a misconfigured push cannot disable a batch
-- of accounts in one shot.
CREATE TABLE scim_pending_changes (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entity          text NOT NULL CHECK (entity IN ('user','group')),
    operation       text NOT NULL CHECK (operation IN ('create','update','delete')),
    external_id     text NOT NULL,
    payload         jsonb NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    applied_at      timestamptz,
    rejected_reason text
);

-- --------------------------------------------------------------- settings --

CREATE TABLE settings (
    key             text PRIMARY KEY,
    value           jsonb NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid REFERENCES users(id)
);

COMMIT;
