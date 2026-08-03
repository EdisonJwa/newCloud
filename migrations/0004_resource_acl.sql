-- 0004_resource_acl.sql
--
-- Makes acl_entries a working part of the permission model (INV-01, §4.1).
--
-- The table has existed since 0001 and no code ever read or wrote it, so the
-- documented three-layer model was really two: a platform role plus "owner_id
-- matches". This migration adds what the layer needs to be usable:
--
--   * one grant per (node, subject), enforced by a partial unique index rather than
--     by hoping callers check. Two rows for the same subject would make the
--     effective permission depend on read order.
--   * created_by, so "who gave them access" is answerable.
--   * an index on the ancestor walk, which is the query every authorization performs.

BEGIN;

ALTER TABLE acl_entries ADD COLUMN IF NOT EXISTS created_by uuid REFERENCES users(id);

-- The two subject shapes cannot share one index because each leaves the other column
-- NULL, and a plain UNIQUE would treat those NULLs as distinct anyway.
CREATE UNIQUE INDEX IF NOT EXISTS acl_entries_node_user_uniq
    ON acl_entries (node_id, user_id) WHERE user_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS acl_entries_node_group_uniq
    ON acl_entries (node_id, group_id) WHERE group_id IS NOT NULL;

-- The membership lookup runs once per grant while resolving permissions.
CREATE INDEX IF NOT EXISTS group_members_user_group_idx
    ON group_members (user_id, group_id);

COMMIT;
