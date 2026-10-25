-- Notifications (competitiveness-analysis-2026-10-25 M4): the in-app inbox.
--
-- A notification is telemetry, not evidence: nothing here is hash-chained (core-scope §8 states
-- the policy for social metadata, and notifications follow it for the same reason — a bell icon
-- is not an accountability record). The table is therefore plain rows with a retention window,
-- pruned by a worker duty like sessions are.
--
-- meta is structured (kind + fields), never pre-rendered text: the client renders it in the
-- user's locale (en, zh-Hans), which the server does not know.

CREATE TABLE IF NOT EXISTS notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    -- dedupe_key collapses repeats while the notification is UNREAD: one quota warning per band,
    -- one expiry reminder per share. Reading the notification re-arms it, which is the behaviour
    -- a bell icon promises (dismissed once, told again the next time it happens).
    dedupe_key text,
    meta       jsonb NOT NULL DEFAULT '{}'::jsonb,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The inbox walk: newest first, keyset on (created_at, id) so same-instant rows page exactly
-- once — the same contract every other listing in the schema follows (ADR-0012).
CREATE INDEX IF NOT EXISTS notifications_inbox_idx
    ON notifications (user_id, created_at DESC, id);

-- The unread-collapse. Partial, so it never constrains read rows or keyless notifications.
CREATE UNIQUE INDEX IF NOT EXISTS notifications_dedupe_idx
    ON notifications (user_id, kind, dedupe_key)
    WHERE dedupe_key IS NOT NULL AND read_at IS NULL;
