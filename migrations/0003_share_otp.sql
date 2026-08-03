-- 0003_share_otp_attempts.sql
--
-- Verification-code hardening (SHARE-02, SHARE-03, T-09).
--
-- Before this, a code was an HMAC ticket: valid for its full TTL, verifiable an
-- unlimited number of times, and with no record of failed attempts. That made the
-- recipient gate an offline-guessable oracle — an attacker with a share link could
-- hammer addresses for fifteen minutes with no throttle and no trace, and a leaked
-- code worked until it expired rather than until it was used.
--
-- Two changes:
--   * share_otp_attempts records failures per (share, address), so a threshold can
--     be enforced. It is pruned by the worker, not by the request path.
--   * share_recipients.consumed_at makes a code single-use. Marking the recipient
--     row rather than the ticket is deliberate: the ticket is stateless, and the
--     authoritative place for "this address has already used its code" is the row
--     that binds address to share.

BEGIN;

CREATE TABLE share_otp_attempts (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    share_id        uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    -- Lowercased address, matching share_recipients.email's citext semantics.
    email           citext NOT NULL,
    attempted_at    timestamptz NOT NULL DEFAULT now(),
    succeeded       boolean NOT NULL DEFAULT false,
    ip              inet,
    -- A short reason, for the audit reader: "bad_code", "expired", "throttled".
    reason          text NOT NULL DEFAULT ''
);

-- The throttle query is "how many failures for this share and address recently",
-- so the index has to lead with both and carry the time.
CREATE INDEX share_otp_attempts_window_idx
    ON share_otp_attempts (share_id, email, attempted_at DESC);

ALTER TABLE share_recipients ADD COLUMN IF NOT EXISTS consumed_at timestamptz;

COMMIT;
