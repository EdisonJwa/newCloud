-- SHARE-05: the rate limit, the fifth and last of the five upload limits.
--
--  文件数     max_uploads            — 0001
--  单文件大小 max_bytes_per_file     — 0001
--  总空间     max_total_bytes        — 0008
--  速率       max_upload_rate       — this migration
--  保留期限   retention_seconds      — 0008
--
-- Why a table rather than a counter on `shares`: the other four limits are totals, so a single
-- column can hold them and a guard in the same UPDATE keeps them exact. A rate is a ROLLING WINDOW,
-- and there is no column that can represent "how many arrived in the last N minutes" — the answer
-- changes with time alone, without any write happening. The honest representation is the events
-- themselves.
--
-- Why not an in-process token bucket: the deployment runs more than one replica (the worker is
-- already written to tolerate a second sweeper), and an in-memory limiter would give each replica
-- its own allowance — so the real limit would be N × replicas and would change whenever a pod was
-- rescheduled. The audit log's own rows are durable for the same reason. A durable limiter also
-- survives a restart, which is what stops "restart the process" from being a way to reset the
-- budget.
--
-- The cost is a row per accepted upload plus one indexed count per request. That is acceptable
-- because uploads are already heavyweight: each one writes a blob and a node. Pruning keeps the
-- table bounded — see the worker's share-rate prune.

ALTER TABLE shares ADD COLUMN IF NOT EXISTS max_upload_rate bigint;
ALTER TABLE shares ADD COLUMN IF NOT EXISTS upload_rate_window_seconds bigint;

COMMENT ON COLUMN shares.max_upload_rate IS
    'SHARE-05 速率: uploads accepted per upload_rate_window_seconds. NULL means no rate limit.';

COMMENT ON COLUMN shares.upload_rate_window_seconds IS
    'Sliding window for max_upload_rate. Only meaningful when max_upload_rate is set.';

CREATE TABLE IF NOT EXISTS share_upload_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    share_id    uuid        NOT NULL REFERENCES shares (id) ON DELETE CASCADE,
    -- ip is recorded for an operator investigating a burst, not for enforcement: enforcement is
    -- per-share, because the requirement's other four limits are per-share and a per-IP limit
    -- would let one visitor open a second link and double their allowance.
    ip          inet,
    bytes       bigint      NOT NULL DEFAULT 0,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE share_upload_events IS
    'Accepted uploads through a share link, used to enforce max_upload_rate. A rolling window cannot be stored as a column because the count changes with the passage of time alone.';

-- The rate check is a count over the window for one share, so the index has to lead with share_id
-- and carry occurred_at second — ordering by the timestamp is what lets the range predicate use it.
CREATE INDEX IF NOT EXISTS share_upload_events_window_idx
    ON share_upload_events (share_id, occurred_at DESC);
