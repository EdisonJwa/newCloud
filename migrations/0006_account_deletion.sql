-- Allow account deletion (ID-08).
--
-- The first migration constrained users.status to active/disabled/pending. Account deletion
-- was not implemented then, and an account removal that *deletes* the row is not viable:
-- nodes.owner_id, shares.owner_id and versions.created_by reference users with NO ACTION, and
-- the audit log refers to the account by id. A tombstone is the only option that keeps the
-- audit trail intact, and a tombstone needs a status the constraint did not allow.
--
-- Widening the constraint rather than dropping it: the closed set is what makes "disable this
-- account" and "typo the status column" different operations, and that property is worth
-- keeping.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status = ANY (ARRAY['active'::text, 'disabled'::text, 'pending'::text, 'deleted'::text]));

-- A tombstone keeps its row but releases the identifiers, so the former address can be
-- re-invited and the former username reused. The placeholder is 'deleted-<uuid>' and this
-- index makes the intent explicit: tombstones are expected to accumulate, and a query for
-- "who is a real account" should never have to pattern-match a string.
CREATE INDEX IF NOT EXISTS users_deleted_idx ON users (id) WHERE status = 'deleted';

COMMENT ON COLUMN users.status IS
    'active | disabled | pending (JIT approval) | deleted (tombstone: row kept so audit and ownership references remain resolvable, identifiers released)';
