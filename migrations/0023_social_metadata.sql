-- Social metadata: favorites, tags, comments (core-scope §4, §5, §6).
--
-- Three tables that look unrelated and share one design decision: all of them are addressed by
-- NODE ID, never by path. A path is a rendering of where a file currently sits; starring a file,
-- tagging it or discussing it are statements about the file. Addressing by path would make every
-- favorite and comment follow the NAME rather than the content, so a rename would silently move
-- somebody else's discussion onto a different document.
--
-- The second shared decision: none of these rows authorise anything. Every read path re-checks
-- the caller's grant on the node, so a stale favorite or a comment thread on a node whose grant
-- was revoked returns nothing rather than leaking that the node exists.

BEGIN;

-- --------------------------------------------------------------- favorites --
--
-- A per-USER row rather than a flag on nodes: `is_starred` on the node would mean two people who
-- share a folder cannot star independently — the first one to press the button would star it for
-- everybody, which is the behaviour that makes a "favorites" feature useless in a team.
--
-- ON DELETE CASCADE because a favorite that outlives its node is garbage: the purge path deletes
-- node rows, and this row must go with them. (The spec asks for the purge SQL to be extended; the
-- foreign key is the stronger form of the same requirement, because it holds for every deletion
-- path including the ones added later.)
CREATE TABLE IF NOT EXISTS favorites (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    node_id    uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, node_id)
);

-- Drives the listing: newest first, per account. The primary key already covers (user_id, ...)
-- lookups for "is this node starred", but not the ordering the listing needs.
CREATE INDEX IF NOT EXISTS favorites_user_recent_idx
    ON favorites (user_id, created_at DESC, node_id);

-- Reverse lookup: "who starred this node" and the node-deletion cascade.
CREATE INDEX IF NOT EXISTS favorites_node_idx ON favorites (node_id);

COMMENT ON TABLE favorites IS
    'Per-user stars. A row here grants nothing: the favorites listing re-checks each node''s grant.';

-- -------------------------------------------------------------------- tags --
--
-- owner_id IS NULL means a SYSTEM tag (admin-managed, visible to everyone). A NOT NULL owner_id is
-- a personal tag. Both live in one table because the node_tags join must not care which it is, and
-- because a personal tag can be promoted to system without rewriting every assignment.
--
-- Uniqueness is on the case-folded name. The lesson migration 0007 taught this codebase (T-12:
-- "Readme" and "README" are one name) applies to labels exactly as it does to filenames: two tags
-- a user cannot tell apart would fragment the filter they exist to drive.
CREATE TABLE IF NOT EXISTS tags (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   uuid REFERENCES users (id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    -- Fixed palette; validated in Go against the same list so the API can reject before writing.
    color      text NOT NULL DEFAULT 'gray',
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid REFERENCES users (id)
);

-- Two partial unique indexes rather than one COALESCE expression: PostgreSQL treats NULLs as
-- distinct in a plain UNIQUE, so a single index would let a system tag be created twice.
CREATE UNIQUE INDEX IF NOT EXISTS tags_system_name_uniq
    ON tags (lower(name)) WHERE owner_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS tags_owner_name_uniq
    ON tags (owner_id, lower(name)) WHERE owner_id IS NOT NULL;

-- Listing a user's tags, and enforcing the per-account cap.
CREATE INDEX IF NOT EXISTS tags_owner_idx ON tags (owner_id, name);

COMMENT ON TABLE tags IS
    'Labels. owner_id NULL = system tag (admin-managed); otherwise personal to that account.';

CREATE TABLE IF NOT EXISTS node_tags (
    node_id   uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    tag_id    uuid NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    tagged_by uuid REFERENCES users (id),
    tagged_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, tag_id)
);

-- The search filter asks "which nodes carry tag T", so the reverse direction needs its own index.
CREATE INDEX IF NOT EXISTS node_tags_tag_idx ON node_tags (tag_id, node_id);

COMMENT ON TABLE node_tags IS
    'Assignment of a tag to a node. Capped per node and per account by the application (core-scope §5).';

-- ---------------------------------------------------------------- comments --
--
-- Soft delete, deliberately: a thread with a hole in it is unreadable, and "a comment was deleted"
-- is itself a moderation fact that has to survive. The row also carries deleted_by so a moderator
-- can see who removed what without reading the audit chain the deletion is recorded in.
--
-- parent_id is a SINGLE level of reply (core-scope §6): it must reference a top-level comment,
-- which the API enforces and the trigger below refuses to violate even for a direct SQL writer.
CREATE TABLE IF NOT EXISTS comments (
    -- seq is the THREAD ORDER, and it is an identity column rather than a timestamp on purpose.
    --
    -- "Oldest first" is what a discussion means, and created_at cannot express it: PostgreSQL's
    -- now() is the TRANSACTION start time, so every comment posted inside one transaction gets an
    -- identical timestamp, and the in-memory backend's clock is coarse enough on some platforms
    -- that several posts in a row share one too. Ordering by (created_at, id) then breaks the tie
    -- by a random UUID, so two replies can appear above the message they answer.
    --
    -- A monotonic sequence is the ordering the reader actually wants, it is total by construction,
    -- and it makes the page cursor a single number instead of a composite — which is also why
    -- ADR-0012's "no row twice" property becomes trivial here.
    seq        bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id    uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    author_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id  uuid REFERENCES comments (id) ON DELETE CASCADE,
    body       text NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    edited_at  timestamptz,
    deleted_at timestamptz,
    deleted_by uuid REFERENCES users (id)
);

-- The thread listing: one node's comments in posting order.
CREATE INDEX IF NOT EXISTS comments_node_seq_idx ON comments (node_id, seq);
-- Replies are fetched by parent for the badge count.
CREATE INDEX IF NOT EXISTS comments_parent_idx ON comments (parent_id) WHERE parent_id IS NOT NULL;
-- The rate limiter counts one author's recent comments.
CREATE INDEX IF NOT EXISTS comments_author_recent_idx ON comments (author_id, created_at DESC);

-- Single-level replies, enforced by the database.
--
-- The API checks this before inserting, so this is the second line of defence rather than the
-- only one — but a schema that permits a two-level thread while the reader renders single-level
-- is a data model that will eventually disagree with its renderer.
CREATE OR REPLACE FUNCTION comments_single_level() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.parent_id IS NOT NULL THEN
        IF NEW.parent_id = NEW.id THEN
            RAISE EXCEPTION 'a comment cannot reply to itself';
        END IF;
        IF EXISTS (SELECT 1 FROM comments WHERE id = NEW.parent_id AND parent_id IS NOT NULL) THEN
            RAISE EXCEPTION 'replies are single-level: parent_id must name a top-level comment';
        END IF;
    END IF;
    RETURN NEW;
END
$fn$;

DROP TRIGGER IF EXISTS comments_single_level_trg ON comments;
CREATE TRIGGER comments_single_level_trg
    BEFORE INSERT OR UPDATE OF parent_id ON comments
    FOR EACH ROW EXECUTE FUNCTION comments_single_level();

COMMENT ON TABLE comments IS
    'Discussion on a node. Plain text only; soft-deleted so the thread and the moderation fact survive.';

-- The rate limit is enforced from this table rather than a counter: "how many were posted in the
-- last hour" changes with the passage of time alone, which no column can represent (the same
-- reasoning as share_upload_events, migration 0009). Counting the comments themselves is also
-- self-pruning: deleting a comment legitimately frees the allowance it consumed.
--
-- A deleted comment still counts, because a spammer deleting their own posts must not reset the
-- budget. That is why the count is not filtered on deleted_at.

COMMIT;
