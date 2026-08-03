-- 0002_download_sessions_and_lifecycle.sql
--
-- Three related additions, all of them about the same idea: a resource is
-- consumed, retained and collected according to rules the database can enforce,
-- not according to a comment in a handler.
--
--   * authorized_download_sessions — the unit of "one download" for a limited
--     share. Counting HTTP requests charged a video seek as a whole download and
--     charged nothing extra when a presigned URL was replayed. A session is bound
--     to one share, one node version and one visitor grant, has a TTL, and is
--     charged exactly once.
--   * nodes.purged_at — marker for permanent deletion. Without it the trash only
--     grows: there was no code path that deleted anything.
--   * version_retention columns on nodes are deliberately not added; retention is
--     a configured policy applied by the sweeper, not per-file state.

BEGIN;

-- --------------------------------------------------- authorized downloads --

CREATE TABLE download_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    share_id        uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    node_id         uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- The visitor's access grant, hashed. Binding the session to the grant is what
    -- stops a second visitor from riding on the first one's paid download.
    grant_hash      bytea NOT NULL,
    -- The node's ETag at the time the session was opened. A download authorised
    -- for version A must not silently serve version B.
    etag            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL
);

-- One live session per (share, node, grant): a second request with the same grant
-- joins the existing session instead of paying again. That is the whole mechanism
-- behind "concurrent Range requests do not each count".
CREATE UNIQUE INDEX download_sessions_unique
    ON download_sessions (share_id, node_id, grant_hash);

CREATE INDEX download_sessions_expiry_idx ON download_sessions (expires_at);

-- --------------------------------------------------------------- lifecycle --

-- purged_at marks a node whose content has been handed to the collector. The row
-- is kept briefly so the audit trail can still name what was deleted, then removed
-- by the same sweep.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS purged_at timestamptz;

CREATE INDEX nodes_purge_idx ON nodes (purged_at) WHERE purged_at IS NOT NULL;

-- The trash sweep needs to find expired subtrees by age; there is already an index
-- on trashed_at, but it is partial on NOT NULL and unordered. Age-based deletion
-- wants the ordering.
CREATE INDEX nodes_trash_age_idx ON nodes (trashed_at) WHERE trashed_at IS NOT NULL;

-- Versions were never indexed by age, so an age-based retention policy would scan
-- the table. This is the index that makes `version_retention_days` affordable.
CREATE INDEX versions_created_idx ON versions (created_at);

COMMIT;
