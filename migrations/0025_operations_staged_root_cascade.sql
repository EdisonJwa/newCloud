-- Let the operation journal follow a node it no longer describes (core-scope §3, ADR-0013).
--
-- `operations.staged_root` was created with the default ON DELETE NO ACTION, which made it the ONLY
-- foreign key to `nodes` that blocked a permanent delete: every other one — content_refs, versions,
-- shares, favorites, tags, comments, acl_entries, lock_tokens, mounts, dead_props,
-- download_sessions — cascades. So a node that had ever been deleted through the API (which writes an
-- operation row, ADR-0013) could be trashed but never purged:
--
--   ERROR: update or delete on table "nodes" violates foreign key constraint
--          "operations_staged_root_fkey" on table "operations" (SQLSTATE 23503)
--
-- That is the empty-trash endpoint failing with a 500 on exactly the files a user is most likely to
-- want gone. Found by running the demo against PostgreSQL and pressing the button; the conformance
-- suite missed it because it trashed nodes through the store, which writes no journal row.
--
-- CASCADE rather than SET NULL, because the journal row is not worth keeping without its node. It
-- records a content-touching operation on a specific subtree: with the subtree permanently deleted
-- there is nothing to resume, nothing to compensate, and no node to name. Keeping a dangling row
-- would also mean an open operation could outlive the thing the recovery pass would repair.
--
-- NO ACTION was never a decision — it is what a column gets when nobody writes the clause — which is
-- why the fix is to write it rather than to work around it at each delete site.

ALTER TABLE operations
    DROP CONSTRAINT IF EXISTS operations_staged_root_fkey;

ALTER TABLE operations
    ADD CONSTRAINT operations_staged_root_fkey
        FOREIGN KEY (staged_root) REFERENCES nodes (id) ON DELETE CASCADE;
