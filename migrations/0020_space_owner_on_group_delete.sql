-- Deleting a group must not delete, or be blocked by, the spaces it owns.
--
-- `nodes.owner_group_id` was declared with the default NO ACTION, which means a group that owns a
-- space cannot be deleted until every space is dealt with by hand — and the failure surfaces as a
-- raw foreign-key violation, which is the worst shape: the operation is impossible, and the reason
-- names a constraint instead of the spaces.
--
-- There is no delete-group path in the product yet (no route, no store method), which is why this
-- was never hit. That is exactly why it is worth fixing now: the next person to write that path
-- should inherit a decided semantic rather than discover one.
--
-- The semantic chosen is SET NULL: deleting a group turns its spaces into ordinary folders, owned
-- by whichever account created them. The files survive, they stop being discoverable through
-- membership, and nothing is destroyed by deleting a group. The alternatives were worse — CASCADE
-- deletes people's data as a side effect of an administrative action, and RESTRICT leaves no way to
-- delete a group that has ever been used.
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_owner_group_id_fkey;
ALTER TABLE nodes
    ADD CONSTRAINT nodes_owner_group_id_fkey
    FOREIGN KEY (owner_group_id) REFERENCES groups(id) ON DELETE SET NULL;
