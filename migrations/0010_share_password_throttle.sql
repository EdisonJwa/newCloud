-- SHARE-02: a password-guess limit, distinct from the OTP one.
--
-- The requirement is explicit that password and OTP verification both need a rate limit, a
-- validity period, an attempt cap and replay protection. OTP had all four. Password verification
-- had none: a wrong password was audited — so the owner could see a burst afterwards — but nothing
-- slowed the burst down, and nothing stopped it. A share password is typically short and
-- human-chosen, so online guessing was the realistic attack, not a theoretical one.
--
-- Why a separate table rather than reusing share_otp_attempts: that table is keyed by
-- (share_id, email) because an OTP is bound to an address. A share password is not bound to any
-- address — the same link is opened by whoever holds it — so keying password attempts by email
-- would create a new counter per typed address and never reach the limit. The correct key is the
-- share, optionally narrowed by client IP for the audit trail.

CREATE TABLE IF NOT EXISTS share_password_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    share_id     uuid        NOT NULL REFERENCES shares (id) ON DELETE CASCADE,
    -- ip is recorded so an operator can tell one attacker from many, but it is NOT the limit key:
    -- a limit per IP would let one attacker rotate addresses, and a limit per share is what the
    -- owner actually configured.
    ip           inet,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    succeeded    boolean     NOT NULL DEFAULT false
);

COMMENT ON TABLE share_password_attempts IS
    'Password verification attempts on a share link, for the SHARE-02 throttle. Keyed by share rather than by address: a share password is not bound to an email, so an address-keyed counter would never fill.';

-- The throttle query counts failures for one share inside a window.
CREATE INDEX IF NOT EXISTS share_password_attempts_window_idx
    ON share_password_attempts (share_id, attempted_at DESC);
