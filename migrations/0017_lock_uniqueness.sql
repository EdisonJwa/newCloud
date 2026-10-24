-- One live lock per node, enforced by the database.
--
-- The insert in lock_pg.go was `INSERT ... WHERE NOT EXISTS (SELECT ...)`, which reads
-- as atomic and is not: two concurrent transactions both see "no live lock" because
-- neither has committed, and both insert. The in-memory backend serialises every
-- transaction, so the shared conformance suite saw a guarantee the PostgreSQL backend
-- did not provide — the same shape as the idp_confirmed_at divergence found earlier
-- (2026-10-24 review, H3).
--
-- A partial index on `expires_at > now()` is not possible: now() is not immutable, and
-- PostgreSQL rejects it in an index predicate. So the index is on node_id alone and the
-- expiry is handled where it belongs — the lock path deletes an expired row for the node
-- before inserting, and the sweeper still removes the rest on its own cadence. Without
-- that delete an expired lock would block a new one until the next sweep, which is the
-- bug this migration would otherwise trade for.
--
-- Collapse any duplicates first: the index cannot be created over data that already
-- violates it, and a deployment that hit the race has exactly that data. Newest wins —
-- the later lock is the one a client is holding and expecting to keep.
DELETE FROM lock_tokens a
 USING lock_tokens b
 WHERE a.node_id = b.node_id
   AND (a.created_at < b.created_at
        OR (a.created_at = b.created_at AND a.token < b.token));

CREATE UNIQUE INDEX IF NOT EXISTS lock_tokens_one_per_node
    ON lock_tokens (node_id);
