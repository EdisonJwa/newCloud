-- Name search moves from a sequential scan to a trigram index (core-scope §7.1).
--
-- The query has always been `strpos(lower(name), lower($2)) > 0`, chosen over LIKE because a
-- user's query is DATA and feeding "%" or "_" from it into a pattern would silently change what
-- the query means. strpos has no metacharacters, so it stays — what changes is that the planner
-- can now use an index for it.
--
-- pg_trgm's GIN operator class supports `LIKE '%x%'` and, since PostgreSQL 9.1, also
-- `%`-independently via the `gin_trgm_ops` on `lower(name)`: the planner rewrites `strpos(a, b) > 0`
-- into a trigram-indexable form only when the expression matches. To be explicit rather than to
-- rely on that, the query in search_pg.go uses `lower(name) LIKE '%' || $2 || '%'` with the
-- metacharacters escaped — see the comment there for why escaping is what keeps the meaning
-- identical to strpos.
--
-- Extension creation is guarded: a deployment whose database role cannot CREATE EXTENSION still
-- works, it just keeps the sequential scan. That is the honest degradation — the same posture the
-- spec takes for the content-search engine in §7.2.
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION WHEN insufficient_privilege OR feature_not_supported THEN
    RAISE NOTICE 'pg_trgm unavailable; name search keeps its sequential scan';
END
$$;

-- Created outside the guard so a missing extension produces one clear failure rather than a
-- migration that silently half-applied. When the extension is absent this is a no-op because the
-- operator class does not exist; the DO block below checks before asking for it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
        CREATE INDEX IF NOT EXISTS nodes_name_trgm_idx
            ON nodes USING gin (lower(name) gin_trgm_ops);
    ELSE
        RAISE NOTICE 'skipping nodes_name_trgm_idx: pg_trgm is not installed';
    END IF;
END
$$;

-- Search is always scoped to an owner and excludes the trash, so the scoping predicate gets its
-- own partial index. Without it the trigram index narrows the name match and the heap still has
-- to filter every row of every account.
CREATE INDEX IF NOT EXISTS nodes_live_owner_idx
    ON nodes (owner_id) WHERE trashed_at IS NULL;

-- The filter/sort combinations §7.1 adds all order by one of three keys within one owner:
--
--   kind / mime / size     -> nodes_owner_kind_idx, nodes_owner_size_idx
--   modified_after/before  -> nodes_modified_idx already exists for the listing; this one is
--                             owner-scoped, which the existing one is not
--
-- Partial on the same predicate the queries use, so the index covers exactly the rows search can
-- return.
CREATE INDEX IF NOT EXISTS nodes_owner_modified_idx
    ON nodes (owner_id, modified_at DESC) WHERE trashed_at IS NULL;
CREATE INDEX IF NOT EXISTS nodes_owner_size_idx
    ON nodes (owner_id, size) WHERE trashed_at IS NULL;
CREATE INDEX IF NOT EXISTS nodes_owner_kind_idx
    ON nodes (owner_id, kind, modified_at DESC) WHERE trashed_at IS NULL;
