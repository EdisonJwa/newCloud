-- SHARE-05: the three upload limits a delivery link was missing.
--
-- The requirement asks for five limits on a share's upload path:
--
--   文件数     max_uploads        — present since 0001
--   单文件大小 max_bytes_per_file — present since 0001
--   总空间     max_total_bytes    — MISSING
--   速率       (rate)             — MISSING
--   保留期限   retention_seconds  — MISSING
--
-- The gap was not theoretical. An anonymous drop-box link with max_uploads=100 and
-- max_bytes_per_file=10 MiB accepts a full gigabyte, because nothing tracks the sum. And a link
-- that accepts uploads forever leaves the owner holding data they never agreed to keep: the quota
-- that matters for a delivery box is space, not count, and the retention that matters is the
-- owner's, not the uploader's.
--
-- Rate limiting is deliberately NOT a column. A per-share rate limit is a property of the request,
-- not of the row: it is a token bucket that has to be consulted before the body is read, it has to
-- survive across replicas through shared state, and storing it in the shares row would mean a
-- write on every request. It is implemented in the server with an in-process limiter whose scope is
-- documented (per-instance, not global) — see internal/server/sharelimit.go. This migration adds
-- only the two limits that are genuinely per-share facts.

ALTER TABLE shares ADD COLUMN IF NOT EXISTS max_total_bytes bigint;
ALTER TABLE shares ADD COLUMN IF NOT EXISTS received_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE shares ADD COLUMN IF NOT EXISTS retention_seconds bigint;

COMMENT ON COLUMN shares.max_total_bytes IS
    'SHARE-05 total-space cap for uploads through this link. NULL means no cap beyond the owner''s quota.';

COMMENT ON COLUMN shares.received_bytes IS
    'Sum of the sizes delivered through this link. Maintained by the same guarded UPDATE that increments upload_count, so the counter and the byte total cannot disagree.';

COMMENT ON COLUMN shares.retention_seconds IS
    'SHARE-05 retention for data delivered through this link. NULL means keep until the owner deletes it. A row is stamped with expires_at at delivery time so the policy in force when the file arrived is the one that applies, even if the link''s policy is changed later.';

-- Delivered files need their own expiry column: the retention that applies to a delivery is the
-- one that was in force when it arrived, not whatever the link says today.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS retain_until timestamptz;

COMMENT ON COLUMN nodes.retain_until IS
    'Set when a file is delivered through a share with retention_seconds. The worker purges rows past this timestamp, using the same trash-then-collect path as a user delete so a recovery window still exists.';

-- The purge query scans for rows past retain_until; a partial index keeps it cheap on a table where
-- almost every row has NULL.
CREATE INDEX IF NOT EXISTS nodes_retain_until_idx
    ON nodes (retain_until)
    WHERE retain_until IS NOT NULL AND trashed_at IS NULL;

-- The byte counter has to start somewhere. Existing shares have delivered content that was counted
-- in upload_count but never in bytes; summing it now would mean joining every delivered node back
-- to its share, which the schema does not record. Starting at zero is the honest choice: the cap
-- applies to what arrives after the deployment, and the comment above records why.
