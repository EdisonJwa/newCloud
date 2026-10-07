-- Audit retention, as a recorded boundary rather than a delete.
--
-- Pruning an append-only hash chain is not "DELETE WHERE ts < cutoff". The chain proves that no
-- event was altered OR REMOVED, so dropping the oldest events leaves a first survivor whose
-- prev_hash points at something that no longer exists, and every later verification reports the log
-- as broken — which looks exactly like tampering.
--
-- A prune mark is the fix: it records that everything up to and including `through_id` was
-- deliberately archived, together with the head hash at that point. Verification starts from the
-- newest mark instead of from genesis, so the remaining chain is still checked end to end and a
-- GAP that nobody recorded is still detectable.
--
-- The mark is also the only honest place to record WHAT happened: the number of events, the hash the
-- archive must contain, and where it was written.

CREATE TABLE audit.prune_marks (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    through_id   bigint NOT NULL,
    head_hash    bytea NOT NULL,
    events       bigint NOT NULL,
    first_ts     timestamptz NOT NULL,
    last_ts      timestamptz NOT NULL,
    archive_path text NOT NULL DEFAULT '',
    pruned_at    timestamptz NOT NULL DEFAULT now()
);

-- One boundary per through_id: re-running a prune that reached the same event must not add a second,
-- contradictory mark.
CREATE UNIQUE INDEX audit_prune_marks_through_idx ON audit.prune_marks (through_id);

-- Append-only like the rest of the audit schema: the application may record a boundary and read the
-- list, and may not rewrite history about what it removed.
GRANT INSERT, SELECT ON audit.prune_marks TO newcloud_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit.prune_marks FROM newcloud_app;
