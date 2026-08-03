-- Sibling-name identity (T-12).
--
-- T-12 lists "大小写/Unicode 名称冲突" as a way to overwrite an unintended target. The rule is that
-- two filenames a user cannot tell apart must be one name: case-insensitive, Unicode-normalised,
-- and with invisible formatting characters removed.
--
-- That comparison existed in Go for the in-memory backend and nowhere for PostgreSQL, where the
-- unique index was `(parent_id, name)` on raw bytes. So "Readme.md" and "README.md" were two
-- siblings in the database while the memory backend refused the second — and a client on macOS or
-- Windows would show one and silently overwrite the other.
--
-- `name_key` is a plain column, not a generated one: the normalisation is a small Go function
-- (NFC + case folding + stripping format characters) that SQL cannot express. Writing it in SQL
-- would be a second implementation that drifts from the first. Every insert sets it via
-- store.nameKey.

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS name_key text;

-- Backfill before making the column NOT NULL. SQL cannot reproduce the Go folding, so existing
-- rows get `lower(name)`, which is what the old index already treated as distinct. Rows the Go
-- function would key differently are corrected the next time they are written.
UPDATE nodes SET name_key = lower(name) WHERE name_key IS NULL;

-- Resolve the collisions the old index allowed.
--
-- Production and this test database both contain sibling pairs like {Readme.md, README.md}: two
-- names that fold to one key. The unique index below cannot be created while they exist, so they
-- have to be reconciled first — and "reconcile" cannot mean "delete one", because a user's file
-- is not the migration author's to drop.
--
-- The rule: the oldest node (lowest created_at, then lowest id for determinism) keeps the plain
-- name; each later colliding sibling is renamed to "<base> (conflict <n>)<ext>". The bytes,
-- ownership, versions and shares are untouched — only the display name changes, and the new name
-- is recorded in the audit log so an operator can find every file that moved and tell the user
-- which one it was.
CREATE TABLE IF NOT EXISTS name_conflict_renames (
    node_id    uuid PRIMARY KEY,
    owner_id   uuid NOT NULL,
    old_name   text NOT NULL,
    new_name   text NOT NULL,
    reason     text NOT NULL,
    renamed_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE name_conflict_renames IS
    'Files renamed by migration 0007 to resolve a case/Unicode name collision (T-12). Kept out of audit.events because that log is hash-chained in Go and a migration cannot extend the chain without breaking its verification.';

DO $$
DECLARE
    dup RECORD;
    victim RECORD;
    n INT;
    base TEXT;
    ext TEXT;
    newname TEXT;
BEGIN
    FOR dup IN
        SELECT parent_id, lower(name) AS k
        FROM nodes
        WHERE trashed_at IS NULL AND parent_id IS NOT NULL
        GROUP BY parent_id, lower(name)
        HAVING count(*) > 1
    LOOP
        n := 1;
        FOR victim IN
            SELECT id, name, owner_id
            FROM nodes
            WHERE trashed_at IS NULL AND parent_id = dup.parent_id AND lower(name) = dup.k
            ORDER BY created_at, id
        LOOP
            -- The first one keeps its name; it is the node that was there first.
            IF n > 1 THEN
                -- Split "name.ext" so the suffix goes before the extension.
                IF position('.' in victim.name) > 1 THEN
                    base := left(victim.name, length(victim.name) - length(split_part(victim.name, '.', -1)) - 1);
                    ext := '.' || split_part(victim.name, '.', -1);
                ELSE
                    base := victim.name;
                    ext := '';
                END IF;
                newname := base || ' (conflict ' || n || ')' || ext;

                UPDATE nodes SET name = newname, name_key = lower(newname) WHERE id = victim.id;

                -- Recorded in its own table rather than in audit.events. The audit log is
                -- hash-chained (prev_hash/hash computed by Go over the previous row), and a
                -- migration cannot forge that chain without producing a log that fails
                -- verification — which would be a worse outcome than no audit entry. This table
                -- is the operator's record of exactly which files the migration renamed.
                INSERT INTO name_conflict_renames (node_id, owner_id, old_name, new_name, reason)
                VALUES (victim.id, victim.owner_id, victim.name, newname,
                        'name collision on case/Unicode fold (T-12)');
            END IF;
            n := n + 1;
        END LOOP;
    END LOOP;
END $$;

ALTER TABLE nodes ALTER COLUMN name_key SET NOT NULL;

-- The uniqueness rule now applies to the key. The old index on the raw name is dropped: keeping
-- it would still allow two siblings whose keys collide, which is the bug this migration fixes.
DROP INDEX IF EXISTS nodes_sibling_name_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS nodes_sibling_namekey_uniq
    ON nodes (parent_id, name_key)
    WHERE trashed_at IS NULL;

COMMENT ON COLUMN nodes.name_key IS
    'Sibling-name identity: NFC + case fold + format characters stripped (store.nameKey). Compare on this, display name.';
