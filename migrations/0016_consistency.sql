-- 0016_consistency.sql — ADR-0013 Phase 1 foundations.
--
-- These tables are additive. No handler reads them in Phase 1; the migration
-- exists so the metadata, journal, staging visibility and lease contracts can be
-- exercised by conformance tests before any live route is migrated.

BEGIN;

-- Resource-state token. It is NOT the content ETag: side-table mutations bump
-- this, while a file's ETag keeps describing its bytes only.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1
    CHECK (revision >= 1);

-- Durable operation journal. A staged node is visible only to the operation that
-- owns it; staged_by is cleared on the subtree root at publish.
CREATE TABLE IF NOT EXISTS operations (
    id              uuid PRIMARY KEY,
    owner_id        uuid NOT NULL REFERENCES users(id),
    kind            text NOT NULL,
    state           text NOT NULL
                    CHECK (state IN ('INTENT','STAGING','PUBLISHING','COMMITTED',
                                     'ABORTED','DISCARDED','NEEDS_OPERATOR')),
    staged_root     uuid REFERENCES nodes(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS operations_nonterminal_idx
    ON operations (updated_at)
    WHERE state NOT IN ('COMMITTED','ABORTED','DISCARDED','NEEDS_OPERATOR');

CREATE INDEX IF NOT EXISTS operations_owner_idx ON operations (owner_id, updated_at DESC);

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS staged_by uuid REFERENCES operations(id);
CREATE INDEX IF NOT EXISTS nodes_staged_idx ON nodes (staged_by) WHERE staged_by IS NOT NULL;

-- Stable references to content that outlives the live path: trash, versions and
-- an operation backup. The tree lifecycle phases need this to restore or purge
-- real bytes; blob rows can point at an existing blobs row.
CREATE TABLE IF NOT EXISTS content_refs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('trash','version','backup')),
    generation      integer NOT NULL DEFAULT 1 CHECK (generation >= 1),
    storage_path    text,
    blob_id         uuid REFERENCES blobs(id),
    size            bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    hash            bytea,
    state           text NOT NULL DEFAULT 'CONTENT_OK'
                    CHECK (state IN ('CONTENT_OK','LEGACY_CONFLICT',
                                     'CONTENT_UNRECOVERABLE','QUARANTINED','PURGING')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, kind, generation),
    CHECK (storage_path IS NOT NULL OR blob_id IS NOT NULL)
);

-- The runtime singleton lease. The row is created once; the owner takes it with
-- a row lock/heartbeat and increments epoch on takeover, so an old process's
-- in-flight publish can be fenced out in the same transaction as its metadata
-- write.
CREATE TABLE IF NOT EXISTS consistency_lease (
    id              smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    owner_id        uuid,
    epoch           bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    expires_at      timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
INSERT INTO consistency_lease (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

COMMIT;
